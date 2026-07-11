# 26. CSP vs Actor Model — Concepts

Go's concurrency is built on CSP, but the actor model is its closest cousin and shows up everywhere from Erlang to Akka to Orleans. The two are easy to conflate because in Go they are implemented with the same two primitives — a goroutine and a channel — yet they make a different decision about one question: where does identity live? Getting that question straight is what lets you decide whether to expose a channel and let callers compose it, or to hide a channel behind an object and let callers only send messages. This file is the conceptual foundation; read it once and the four exercises that follow build both styles, the same concurrent bank account twice, and a worker pool whose behavior measurably diverges depending on which model you pick.

## Concepts

### CSP: identity lives in the channel

CSP — Communicating Sequential Processes, formalized by Tony Hoare in 1978 — models a concurrent system as independent sequential processes that share nothing and communicate only by passing values through channels. In Go the processes are goroutines and the channels are the `chan T` primitive. The defining rule, the one printed on the Go proverbs slide, is "do not communicate by sharing memory; share memory by communicating": the channel is the synchronization mechanism, not a lock around shared state.

The consequence that matters for design is that the channel, not the goroutine, is the thing with an identity. A goroutine does not own a channel. Any goroutine holding the reference can send on it, receive from it, pass it into a struct, hand it to another goroutine, or (if it holds the send side) close it. Because identity sits in the channel, topology is determined by who holds which reference: fan a single channel out to a pool of receivers, fan many producers in to one channel, or splice channels together with `select`. The wiring is explicit and composable, and it is visible in the types — a function that only sends takes a `chan<- T`, a function that only receives takes a `<-chan T`, and the compiler enforces the direction.

### Actor model: identity lives in the actor

The actor model — Hewitt, Bishop, and Steiger, 1973 — makes the opposite choice. Identity belongs to the actor. Each actor has a stable address; you send a message to the address, not to a queue you can see. The actor owns a private mailbox, pulls one message at a time, and runs its behavior to completion before taking the next. Because nothing outside the actor can touch its state and messages are processed one at a time, an actor has no internal data races by construction — the single-consumer mailbox is the serialization point. In Erlang and Akka the address is a first-class value (a PID, an `ActorRef`) that can be sent in messages and survive restarts under a supervisor.

In Go there is no built-in actor runtime, so you simulate one: wrap a channel in a struct, keep the channel unexported, and expose only methods. The struct pointer becomes the actor's address; the methods are how you "send" to it; the goroutine looping over the mailbox is the actor's behavior. From outside the package there is no channel to see, close, range over, or select on — exactly the encapsulation the actor model promises.

### The same machine, a different seam

Here is the honest part most write-ups skip: in Go both models compile down to the identical machine — one goroutine looping over one channel with `select`. The CSP chat room and the actor chat room in this lesson share that skeleton line for line. What differs is the seam you expose. CSP puts the channel in the public API so callers can compose it; the actor hides the channel and publishes methods so callers cannot. That is not a cosmetic distinction. Exposing the channel buys composability — a caller can `select` your channel against a timeout, fan it into a pipeline, or close it to signal completion. Hiding it buys an invariant — callers cannot accidentally close your channel, leak a second consumer that steals half your messages, or range over a mailbox you intended only you to drain. You are trading composability for encapsulation, and the right answer depends on whether the channel is part of the contract or an implementation detail.

### Channels: rendezvous, buffering, and select

A channel carries data and synchronization in one operation. An unbuffered channel is a rendezvous: the sender blocks until a receiver is ready, so a completed send is proof the value was handed off. A buffered channel of capacity N decouples sender and receiver for up to N items; it is a bounded queue, and the bound is the point — it provides backpressure. When the buffer fills, senders block, which throttles a fast producer to the speed of a slow consumer instead of letting an unbounded queue grow until the process runs out of memory. A mailbox is just a buffered channel viewed through the actor lens, and choosing its capacity is choosing how much a slow actor may fall behind before its senders feel it.

`select` is the multiplexer. It waits on several channel operations at once and proceeds with whichever is ready, picking uniformly at random among those simultaneously ready. Three uses recur throughout the exercises. A `select` with a `<-ctx.Done()` case gives a goroutine a clean cancellation path without anything resembling thread interruption. A `select` with a `default` case turns a blocking send or receive into a non-blocking try, which is how an owner goroutine refuses to be stalled by one slow consumer. And a `select` over several inbound channels is how a CSP-style server multiplexes distinct operation types without a single tagged-union message.

### Request/reply over a one-shot reply channel

Both styles need a way to ask a question and get an answer back, because the state lives inside one goroutine and the caller is in another. The idiom is a reply channel carried in the request: the caller makes a `chan T` (capacity 1), puts it in the message, sends the message, then blocks receiving on its own reply channel; the owner computes the answer and sends it back. Make the reply channel buffered with capacity 1 so the owner's send never blocks even if the caller has wandered off — the owner drops its single value into the buffer and moves on instead of being held hostage by a caller that stopped listening. This one pattern turns a fire-and-forget mailbox into a synchronous API (`Balance() int64`) without exposing any shared memory.

### When to choose which

Reach for raw CSP when the channel is genuinely part of the contract: the caller will `select` it against other channels, fan it out to a pool, splice it into a pipeline, or close it to signal end-of-stream. The topology is the feature, and hiding it behind methods would only force callers to rebuild it badly. Reach for the actor style when a goroutine owns private mutable state that must never be touched concurrently and the channel is pure plumbing. Exposing the mailbox there is a liability, not a feature: it lets a caller close it, double-consume it, or select on it in a way that violates single-ownership. A useful tie-breaker: if you find yourself writing documentation begging callers not to close or range over a channel you returned, you wanted an actor.

### Where the two models actually diverge: work distribution

The deepest practical difference surfaces when you distribute work, and the last exercise measures it. A CSP shared queue — one channel, many workers ranging over it — is pull-based: a task is taken by whichever worker is free, so a slow or stalled worker simply stops pulling and the others absorb its share. That is dynamic load balancing, and it falls out of the topology for free. The actor approach of one mailbox per worker with a dispatcher assigning tasks round-robin is push-based and statically partitioned: each task is committed to a specific worker's mailbox before that worker's load is known, so a stalled worker leaves its pre-assigned tasks stranded behind it while idle workers cannot help — classic head-of-line blocking. Neither is wrong. Pull-based shared queues maximize throughput under uneven load; per-actor mailboxes give you per-entity ordering, isolation, and a natural place to apply backpressure or supervision. Knowing which property you are buying is the whole point of understanding the two models rather than memorizing one.

## Common Mistakes

### Believing the actor model removes the need to reason about deadlock

Single-message-at-a-time processing eliminates data races on an actor's own state, but it does not eliminate deadlock. If actor A sends a synchronous request to actor B and blocks on the reply while B is blocked sending a synchronous request back to A, both wait forever. Encapsulation is not freedom from protocol design; the message ordering still has to be acyclic or buffered.

### Exposing the mailbox and calling it an actor

Putting `Mailbox chan Message` as an exported field, or returning the channel from a constructor, hands every caller the ability to close it, range over it, or add a second consumer that steals half the messages. The single-owner, sequential-processing guarantee that justifies the actor model is gone the moment the channel is visible. Keep the channel unexported and publish methods.

### An unbounded or oversized mailbox that hides backpressure

Making the mailbox capacity huge so senders "never block" converts backpressure into unbounded memory growth: a producer faster than the actor fills the buffer until the process is killed. A bounded mailbox that occasionally blocks the producer is the system telling you the consumer cannot keep up — that is information, not a bug to paper over.

### A blocking send inside the owner's loop stalls every client

When the owner goroutine sends to a consumer's channel with a bare `ch <- v` and that consumer is slow, the owner blocks and stops serving everyone else. Inside an owner loop, broadcasts and fan-outs to consumers should use a non-blocking `select { case ch <- v: default: }` so one slow consumer is dropped or skipped rather than freezing the whole actor.

### Sending after cancellation

Once the owner goroutine has returned (its context was cancelled, or it was told to stop), nothing drains its channel. A later send on an unbuffered or full channel blocks forever, and a `close` followed by a send panics. Establish a shutdown order: stop accepting new messages, drain or abandon in flight, then signal done — and have callers confirm the owner has exited before sending again.

Next: [01-csp-chat-room.md](01-csp-chat-room.md)
