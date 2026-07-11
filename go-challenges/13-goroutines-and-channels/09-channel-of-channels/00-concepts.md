# Channel Of Channels and Request-Reply Services — Concepts

The `chan chan T` idiom — a channel whose elements are themselves channels — is
how idiomatic Go builds lock-free serialized services (actors) and worker
dispatchers without a single mutex. A senior engineer reaches for it whenever a
component owns mutable state and must serialize access to it: a monotonic
sequence or ID allocator, an admission-controlled job dispatcher, an in-process
rate limiter, a cache coordinator. One goroutine owns the state and reads a
stream of requests; each request carries its own private reply channel; the
goroutine answers on that channel. The pattern is deceptively small, and the
whole difficulty lives in the production concerns wrapped around it: buffering
the reply so a timed-out caller never leaks the worker, threading context
deadlines through both the send and the receive, draining gracefully versus
stopping hard on shutdown, shedding load when the inbox fills, scaling one
serializing goroutine to a competing-consumer pool, and reading the actor's own
internal state without reintroducing the lock you just eliminated. This file is
the conceptual foundation for the nine independent exercises that follow; read
it once and each exercise becomes a variation on one clear model.

## Concepts

### A channel of channels is literally that

`chan chan T` is a channel whose values are channels of `T`. Nothing exotic: a
channel is a first-class value in Go, so it can be sent over another channel,
stored in a struct field, or returned from a function. Two distinct shapes use
this. In the request-reply shape, the outer channel carries requests and each
request *contains* an inner reply channel — the type is `chan Request` where
`Request` has a `Reply chan Response` field, which is a channel-of-channels by
composition. In the load-balancer shape, the outer channel is literally
`chan chan Request`: an idle worker advertises readiness by pushing its own
private request channel onto the shared channel, and the balancer pops a ready
worker's inbox to hand it the next job. Both are the same structural idea — a
channel that carries the means to reply — applied to two different control
flows.

### Reply-channel ownership is what makes it race-free

The discipline that keeps the pattern correct is clear ownership. The caller
creates and owns the inner reply channel, buffers it with capacity one, and
passes it inside the request. The service owns the outer request channel and
never closes the reply channel it was handed — it only sends one value on it.
Because exactly one goroutine ever sends on a given reply channel and exactly one
ever receives, there is no shared mutable state on that path and therefore no
race. The service owns lifecycle of the outer channel; the caller owns lifecycle
of each inner channel. Violating this split — for example, closing the request
channel from a caller to signal shutdown — is where panics and races appear.

### The single-goroutine service is an actor

When exactly one goroutine reads the request channel and that goroutine is the
only code that touches the mutable state, the state needs no mutex. A counter, a
map, a rate-limiter's token bucket — all live as ordinary local variables inside
the run loop, mutated in place as requests arrive. This is the actor model:
serialize access by construction rather than by locking. The payoff is not just
simplicity; it is that `go test -race` proves the absence of a data race
precisely because only one goroutine ever reads or writes the state. A
hundred concurrent callers can hammer an ID allocator and every returned ID is
unique and gap-free, with no `sync.Mutex` anywhere, because the increment happens
in a single goroutine that processes one request at a time.

### Buffer the reply channel with capacity one

The reply channel must be buffered with capacity one. The reason is a failure
mode, not a style preference. Consider a caller that sends its request, then
waits for the reply under a deadline, and the deadline fires first: the caller
returns and stops receiving. If the reply channel were unbuffered, the worker —
which finishes the request a moment later — blocks forever on the send, because
there is no receiver and never will be. That worker goroutine is now leaked, and
under sustained caller timeouts the leak compounds until the process runs out of
memory or its worker pool is fully wedged. A capacity-one buffer makes the
worker's send always complete: it drops the value into the buffer, the buffer is
garbage-collected with the abandoned channel, and the worker loops to the next
request. One slot is exactly enough because there is exactly one reply per
request.

### Context must be threaded through both the send and the receive

A production `Call` takes a `context.Context` and honors it in two places, not
one. The inbox may be full, so the send must select on `ctx.Done()`; the worker
may be slow, so the reply wait must also select on `ctx.Done()`. Ignoring context
on either side lets a slow backend block an HTTP handler past its deadline budget.
When a context branch fires, return the reason via `context.Cause(ctx)`: for a
`context.WithTimeout` context the cause is `context.DeadlineExceeded`, for a plain
cancellation it is `context.Canceled`, and for a `context.WithCancelCause` context
it is whatever error the canceller passed. `context.Cause` is the modern way to
surface *why* a call was abandoned, which matters when the reason is
load-shedding versus a client disconnect versus a timeout.

### Shutdown has two distinct shapes

Stopping a running actor is not one operation but two. A hard stop rejects
in-flight work: it signals the run loop to return immediately, and callers still
waiting on a reply get a shutting-down error. A graceful drain stops accepting
new requests but finishes everything already accepted, then closes. A service
behind SIGTERM usually wants graceful drain with a deadline: finish the requests
you promised to finish, reject new ones, and fall back to a hard stop if the
drain runs long. In both shapes, `Shutdown` must be synchronous — it closes a
signal channel and then *waits on a done channel* that the run loop closes on its
way out. Signalling without joining is the classic half-shutdown: `Shutdown`
returns while the goroutine is still running, which leaks goroutines and races
tests.

### Backpressure needs a bounded inbox and load shedding

An unbounded request channel hides overload until the process runs out of memory.
The fix is a bounded (buffered) inbox plus a non-blocking admission path: a
`TryCall` whose send is a `select` with a `default` that returns a busy error
when the buffer is full. This is admission control — the service sheds load
deterministically instead of queueing without limit. `len(inbox)` and
`cap(inbox)` expose current depth and capacity so an operator or an autoscaler
can see how close the service runs to its shedding threshold. Shedding early and
loudly is more resilient than an unbounded queue that absorbs a spike and then
collapses under memory pressure.

### The chan chan Request load balancer (Rob Pike's pattern)

The canonical channel-of-channels balancer works like a taxi rank. Each worker
has a private request channel. When a worker is idle it advertises by sending its
own channel onto a shared `chan chan Request`. The balancer, holding a stream of
incoming jobs, receives the next ready worker's channel and sends the job
straight to it. Because a worker only re-advertises after it finishes and returns
to the loop, the ready channel naturally orders workers least-recently-idle: the
worker that has been free longest is at the front. You get load balancing with no
scheduler, no counters, and no locks — just the FIFO discipline of a channel.

### State ownership dictates whether you can fan out

Whether you may scale from one goroutine to many is decided entirely by state
ownership. A stateless handler — one that maps a request to a response using only
the request's own data and immutable shared config — can safely run as a
competing-consumer pool: `W` worker goroutines all reading the same request
channel, each answering on the request's own reply channel. Per-request reply
semantics are preserved because the reply channel travels with the request. But a
handler that owns mutable state (the allocator's counter, the rate limiter's
bucket) must keep exactly one goroutine touching that state, or move to explicit
synchronization. The moment two workers mutate the same variable without a lock,
the data race the actor design eliminated comes right back.

### Observability without locks: query through the same loop

Reading an actor's internal metrics — in-flight count, total processed, queue
depth — from another goroutine via shared struct fields reintroduces the race the
actor avoided. The idiomatic fix is to make the metrics query just another kind of
request on the same channel: send a stats request carrying its own reply channel,
and the run loop answers it in the same `select` that handles work. Because the
answer is produced by the one goroutine that also owns the counters, the snapshot
is consistent and never torn, and there is still no mutex. The cost is that a
stats query can only be answered *between* requests, which is exactly the
consistency guarantee you want.

## Common Mistakes

### Unbuffered reply channel

Wrong: `reply := make(chan Response)`. When the caller times out and stops
receiving, the worker's send blocks forever and the goroutine leaks; under
sustained timeouts the leak compounds until the pool is wedged or the process
OOMs. Fix: `make(chan Response, 1)` so the worker's send always completes even
with no receiver.

### Closing quit without waiting on done

Wrong: `Shutdown` closes the quit channel and returns. The run loop may still be
executing after `Shutdown` returns, leaking a goroutine and racing tests that
assume the service has stopped. Fix: `close(quit); <-done`, where the run loop
`defer close(done)` on exit, so `Shutdown` is synchronous.

### Blocking on the inbox send with no escape

Wrong: `s.requests <- req` with no `select`. A full or unreceived request channel
deadlocks the caller. Fix: select the send against `ctx.Done()` or a quit channel,
or use a non-blocking `default` for load shedding.

### Ignoring context while awaiting the reply

Wrong: `resp := <-reply` with a bare receive. A slow worker blocks the caller past
its deadline even though the caller passed a context. Fix: select the reply
receive against `ctx.Done()` and return `context.Cause(ctx)`.

### Fanning out a stateful actor

Wrong: running `W` workers over the same request channel when the handler mutates
shared state (a counter, a map) with no synchronization — this reintroduces the
data race the single-goroutine design eliminated. Fix: keep stateful handlers to
one goroutine; only fan out stateless handlers.

### Closing the request channel to signal shutdown

Wrong: a caller (or the sender side) closes the request channel to tell the
service to stop. A subsequent send on the closed channel panics, and any caller
racing the close crashes. Fix: use a separate quit channel and let the owner
control lifecycle; never close a channel from the receiving/sending side that
others still use.

### Reading metrics from shared fields

Wrong: another goroutine reads `s.processed` or `s.inFlight` directly to build a
metric. That is a data race against the run loop's writes and yields torn or stale
values. Fix: route the query through the same select loop as a stats request, so
reads are serialized with writes.

### Treating an unbounded inbox as resilience

Wrong: an unbuffered-cap or ever-growing request channel "so we never drop a
request." Under sustained overload this just delays and worsens the failure into
OOM. Fix: bound the inbox and shed load with a non-blocking `TryCall` returning a
busy error.

### Assuming range over the request channel ends on shutdown

Wrong: `for req := range s.requests` as the run loop, expecting it to end at
shutdown. If the channel's owner never closes it, the range blocks forever. Fix:
drive termination with a `select` on a quit channel plus an explicit drain of any
buffered requests before returning.

Next: [01-serialized-actor-service.md](01-serialized-actor-service.md)
