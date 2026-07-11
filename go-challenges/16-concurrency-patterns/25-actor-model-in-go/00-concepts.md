# 25. Actor Model in Go — Concepts

The actor model is a concurrency model whose unit of computation is an *actor*:
an entity with private state, a mailbox (a message queue), and a behavior that
processes one message at a time. Actors never share memory; they communicate
only by sending messages to one another's mailboxes. Because each actor's state
is touched by exactly one goroutine, there are no data races to guard against
and no mutexes to forget. Go maps onto this model almost without translation: a
goroutine is the actor, a channel is the mailbox, and a `for range` over that
channel is the sequential message loop. This file is the conceptual foundation
for the chapter; read it once and the three exercises that follow — a reusable
actor system, a stateful account actor that serializes every mutation through
its mailbox, and a supervisor that restarts a child on failure — become small,
independent applications of the same handful of ideas.

## Concepts

### Why Single Ownership Removes the Lock

When two goroutines read and write the same variable, every access must be
ordered by a mutex, an atomic, or a channel, or the program has a data race —
undefined behavior that the `-race` detector exists to find. The actor model
removes the shared variable instead of guarding it. An actor's state lives in
local variables of the goroutine running its message loop; no pointer to that
state escapes to any other goroutine. Mutation happens only inside the handler,
and because the loop pulls one message at a time off the channel, the handler is
never re-entered concurrently. The synchronization is structural: the channel
both delivers the work and serializes it, so the body of the handler can be
written as ordinary single-threaded code. This is the practical payoff that the
account exercise demonstrates — a balance updated by hundreds of concurrent
callers, with not a single mutex and a clean `-race` run, because the balance is
a plain `int64` owned by one goroutine.

### The Mailbox Is a Buffered Channel, and Its Capacity Is a Policy

A mailbox is `make(chan Message, capacity)`. The capacity is not a detail; it is
the backpressure policy. With capacity zero (unbuffered) a send blocks until the
actor is ready to receive, tightly coupling sender and receiver in lockstep.
With a large capacity the sender rarely blocks but a fast producer can pile up
unbounded work and exhaust memory. A bounded, non-trivial capacity is the usual
choice: the sender proceeds while there is slack, and once the buffer fills, the
send blocks and that blocking *is* the backpressure — it propagates the
slowdown back to whoever is producing too fast. Choosing the number is an
engineering judgment about burst size versus acceptable latency and memory, not
a value to leave at "as large as possible".

### FIFO Ordering Holds Per Sender, Not Globally

A Go channel is first-in-first-out, so messages from a single sender are
received by the actor in the order that sender sent them. This is the only
ordering guarantee the actor model offers, and it is enough for most protocols.
Messages from *different* senders interleave in an order decided by the
scheduler and are not reproducible from run to run. Designs that need a global
total order must build it explicitly — for example by routing all the relevant
messages through one sender, or by stamping each message with a sequence number
the receiver sorts on.

### Request/Reply: Getting an Answer Back Without Sharing State

Fire-and-forget messages are the simple case. Often a caller needs a result: the
current balance, an acknowledgment, an error. The actor must not hand back a
pointer into its own state, because that would let the caller read state the
actor is concurrently mutating — the exact race the model exists to avoid.
The idiomatic answer is the *reply channel*. The caller creates a fresh
single-element channel, puts it inside the request message, sends the message,
and blocks receiving on that channel. The actor processes the request, computes
a value that is a copy, and sends that copy down the reply channel. State is
never shared; only immutable values cross the boundary. A one-element buffered
reply channel (`make(chan reply, 1)`) is the common choice so the actor's send
never blocks even if the caller has not yet started receiving. This pattern
turns an asynchronous mailbox into a synchronous-looking method call, and it is
how the account actor exposes `Deposit`, `Withdraw`, and `Balance` as ordinary
blocking methods that are nevertheless lock-free and linearizable: each method
is one round trip through the mailbox, and because the mailbox serializes
requests, the operations take effect in some single, well-defined order.

### Failure Isolation and the Let-It-Crash Philosophy

In a lock-based design an unexpected panic can leave a mutex held or a data
structure half-updated, poisoning the whole process. Actors localize failure: a
panic in one actor's handler unwinds only that actor's goroutine. The minimal
defense is a deferred `recover` inside the message loop so that one bad message
does not tear down the actor; the actor logs the failure and moves on to the
next message. The more powerful stance, inherited from Erlang/OTP, is
*let it crash*: do not try to recover every conceivable corrupt state inside the
actor; instead let the actor die and have a dedicated *supervisor* restart it
from a known-good initial state. The supervisor owns the lifecycle — it spawns
children, watches for their failure, and applies a restart policy — while the
children own the work. Restarting from a clean slate is often more reliable than
trying to repair arbitrary corrupted state in place, because the set of clean
initial states is small and well understood while the set of corrupt states is
not. The supervisor exercise builds exactly this: a parent that respawns a
child's behavior from a factory function so the restarted child begins with
fresh private state, leaving any sibling actors untouched.

### Lifecycle: Starting, Draining, and Stopping Without Leaks

An actor goroutine that is never told to stop is a goroutine leak. Two channels
express a clean lifecycle. The mailbox carries work and, when closed, signals
"no more messages" — a `for range` over a closed channel drains everything
already buffered and then ends the loop, so closing the mailbox is a graceful
shutdown that processes pending work first rather than dropping it. A separate
`done` channel, which the actor closes from a `defer` as its loop exits, lets a
caller block until the actor has truly finished. An alternative to closing the
mailbox is a sentinel "poison pill" message: the actor recognizes it and returns
from the loop. The pill rides the same FIFO queue as ordinary messages, so every
message sent before it is processed before the actor stops. Whichever mechanism
is used, the rule that prevents panics is that exactly one party owns closing the
mailbox, and no `Send` may run after that close — in a registry-based system the
actor is removed from the registry before its mailbox is retired, so a late
`Send` returns an error rather than panicking on a closed channel.

## Common Mistakes

### Letting a Pointer to Actor State Escape

Wrong: returning, from a handler or a reply, a pointer or a slice header that
aliases the actor's own state, then reading it from the calling goroutine.

What happens: the caller and the actor now both reference the same memory; the
actor keeps mutating it on later messages while the caller reads it. The
`-race` detector flags it, and even when it does not fire, the value the caller
sees is whatever the actor happened to leave there. The whole guarantee of the
model is lost.

Fix: reply with copies of immutable values, never with references into live
state. Return an `int64`, a freshly allocated slice, or a struct value — nothing
the actor will touch again.

### Blocking the Handler on Slow I/O

Wrong: performing a slow database query or network call directly inside the
message handler.

What happens: the handler processes one message at a time, so a slow call stalls
every queued message behind it. The mailbox fills, senders block, and one slow
operation becomes a system-wide latency spike.

Fix: do the slow work in a goroutine spawned from the handler, and have that
goroutine send the result back as another message. The actor stays responsive to
its mailbox while the work proceeds elsewhere.

### Unbounded Mailbox Capacity

Wrong: choosing a buffer so large that `Send` effectively never blocks, on the
theory that blocking is always bad.

What happens: when the producer outruns the consumer, messages accumulate with
no ceiling and the process runs out of memory. The blocking was the safety valve
that an unbounded buffer removes.

Fix: pick a capacity that reflects the largest backlog you are willing to hold.
Let a full mailbox block the sender so backpressure flows upstream, or, where
dropping is acceptable, explicitly drop and count the loss instead of buffering
without limit.

### Recovering a Panic and Then Reusing the Corrupt State

Wrong: catching a panic inside the handler and continuing to use the same
in-memory state as though nothing happened, when the panic occurred halfway
through a multi-field update.

What happens: the state is now partially modified and internally inconsistent;
subsequent messages compute on garbage and the corruption spreads silently.

Fix: decide deliberately. If a single message can be discarded safely, recover
and move on. If a failure means the actor's state may be inconsistent, prefer
let-it-crash: end the actor and have a supervisor restart it from a clean
initial state built by a factory, rather than nursing unknown corruption.

### Closing the Mailbox While Senders Still Hold It

Wrong: closing an actor's mailbox from a shutdown path while other goroutines
may still call `Send`.

What happens: a send on a closed channel panics, taking down the sender.

Fix: establish a single owner of the close and a happens-before edge that
guarantees no send runs after it — wait for all senders to finish (a
`sync.WaitGroup`) before closing, or remove the actor from a registry so further
sends are rejected with an error before the close happens.

---

Next: [01-actor-system.md](01-actor-system.md)
</content>
