# 12. Bridge Channel Pattern — Concepts

A bridge flattens a *stream of streams* into a single stream. Its input is a channel whose every value is itself a channel: `<-chan (<-chan T)`. Its output is a plain `<-chan T` carrying every value produced on every inner channel, presented to one consumer as a single sequence. The pattern exists because the alternative — a consumer writing nested `for inner := range outer { for v := range inner { ... } }` loops everywhere — leaks the stream-of-streams shape into every call site, and because the inner channels frequently arrive *dynamically over time* (a new connection, a new pipeline stage, a re-established subscription) rather than as a fixed slice you could simply concatenate. This file is the conceptual foundation. Read it once and you will have everything needed for the three exercises, which build the core bridge and then two senior, real-world uses of it as independent, self-contained Go modules.

## Concepts

### A Bridge Flattens `<-chan (<-chan T)` Into `<-chan T`

The shape is the whole idea. The outer channel hands the bridge one inner channel at a time. The bridge reads the current inner channel to exhaustion, then receives the next inner channel from the outer channel and reads that, and so on, copying every value it sees onto a single output channel. A consumer never touches the nesting: it writes `for v := range Bridge(done, outer)` and receives a flat sequence, exactly as if the inner channels had been one channel all along.

The natural signature is generic: `Bridge[T any](done <-chan struct{}, chanStream <-chan (<-chan T)) <-chan T`. Generics matter here because the bridging logic is entirely about channel lifecycle and contains nothing type-specific; the same loop must serve `int`, `string`, a message struct, or a page of records without being copied per element type.

### The Output Order Is Deterministic Concatenation, Not Interleaving

A bridge does not merge or interleave inner channels the way a fan-in does. It drains the *current* inner channel completely before it even receives the next one from the outer channel. The result is a concatenation: all of the first inner channel's values in their original order, then all of the second inner channel's values, and so on, in the order the inner channels themselves appeared on the outer channel. Within one inner channel, order is preserved; across inner channels, order follows the outer channel's order. This determinism is exactly why a bridge is the right tool when a single consumer must process each sub-stream as a contiguous, ordered unit — one connection's messages together, one pipeline stage's results together — rather than a scramble of values from every source at once.

The flip side is the serialization cost. Because the bridge will not advance to the next inner channel until the current one closes, a slow or unbounded inner channel blocks every later one behind it. That is the correct behavior when sub-streams are bounded and must be ordered, and the wrong behavior when sub-streams are long-lived and should progress independently — that latter case is a fan-in, not a bridge. Choosing a bridge is choosing ordered, one-at-a-time consumption on purpose.

### The Outer Channel Owns the Lifecycle; the Output Closes With It

The bridge runs a single goroutine that owns the output channel and is the only sender on it, so it is also the only thing that may close it. The termination rule is simple: when the outer channel is closed and drained, there are no more inner channels to read, so the bridge closes its output and the consumer's `range` loop ends. A correct bridge `defer close(out)` so that *every* return path — normal end of the outer channel, and cancellation — closes the output exactly once. Forgetting that close strands the consumer's `range` forever after the last value.

### Cancellation Needs an Explicit `done` Channel

A bridge that only watches its two channels cannot be interrupted: a consumer that decides to stop early has no way to tell the bridge, and the bridge's goroutine (and every producer feeding it) would leak. The pattern threads an explicit `done <-chan struct{}` and selects on it at both blocking points: the outer receive (waiting for the next inner channel) and the inner forward (waiting to hand a value to the consumer). Closing `done` makes both selects return immediately, the goroutine runs its deferred close, and the producers — which must themselves select on the same `done` while sending — unwind too. Cancellation is only effective if `done` reaches every blocking operation in the chain, not just the bridge's two.

This is also why the race detector earns its place in the verification. The bridge sends on the output from one goroutine while the consumer may close `done` from another; the handoff between them is a channel operation, which is properly synchronized, but the surrounding producer goroutines and any shared bookkeeping are exactly where an accidental unsynchronized access hides. Running the tests with `-race` is what proves the cancellation path touches nothing without a happens-before edge.

### The Nil Inner Channel Is a Real Hazard

It is easy to assume that ranging over a nil channel is harmless or terminates immediately. It does not: a receive on a nil channel blocks *forever*, and `for v := range stream` on a nil `stream` therefore hangs the bridge permanently, stalling every later inner channel behind it. A robust bridge guards against it explicitly — after receiving an inner channel from the outer channel, it checks `if stream == nil { continue }` and moves on to the next one. The guard is mandatory precisely because the language gives you no automatic escape; the nil case is not skipped for you. A bridge that streams sub-channels assembled from heterogeneous sources will eventually be handed a nil, and the guard is the difference between skipping it and deadlocking.

### Inner Channels Arrive Dynamically, and That Is the Point

If every inner channel were known up front you could `append` their values into one slice and skip the pattern. The bridge earns its keep when inner channels are *produced over time*: each new client connection contributes a fresh `<-chan Message`; each phase of a paginated or staged pipeline produces its own `<-chan Record`, and the number of phases is not known until a cursor runs out. The outer channel is the registration point — a producer sends a new inner channel onto it the moment one exists — and the bridge turns that open-ended arrival of channels into a single, orderly stream a downstream consumer can `range` over without ever learning how many sub-streams there were or when each began. The two senior exercises build exactly these two cases: per-connection message streams flattened for one ordered consumer, and dynamically created pipeline stages — each stage itself a channel — sequenced into one result stream.

## Common Mistakes

### Treating the Inner Channel as a Single Value

Reading one value from each inner channel and then moving on delivers only the first element of every sub-stream and silently drops the rest. The bridge must `range` the current inner channel until it closes before it receives the next inner channel from the outer channel. The inner loop is not optional; it is the flattening.

### Omitting the `done` Case on Either Select

A bridge has two places it can block: receiving the next inner channel from the outer channel, and forwarding a value to the output. If only one of them selects on `done`, cancellation works in some states and hangs in others — close `done` while the bridge happens to be parked on the *other* operation and the goroutine leaks. Both selects must include `case <-done: return`, and every producer feeding an inner channel must select on the same `done` while sending, or cancellation stops at the first blocked producer.

### Forgetting to Close the Output

The bridge owns the output channel and is its only sender, so it must close it on every exit path. The clean way is `defer close(out)` at the top of the goroutine, which covers the normal end of the outer channel and the cancellation return alike. Without it, the consumer's `for v := range out` blocks forever after the final value, a hang that looks exactly like a slow producer and is maddening to diagnose.

### Assuming a Nil Inner Channel Is Skipped Automatically

Ranging over a nil channel blocks forever; it does not terminate and is not skipped for you. A bridge that may receive a nil inner channel must test `if stream == nil { continue }` explicitly before ranging, or one nil value on the outer channel deadlocks the entire flattened stream.

### Reaching for a Bridge When You Meant a Fan-In

A bridge drains one inner channel fully before the next, so long-lived concurrent sub-streams starve behind whichever one is currently being drained. If the goal is to interleave values from many sources that all make progress at once, that is a fan-in (a `select` or a per-source forwarding goroutine), not a bridge. Use a bridge only when ordered, one-sub-stream-at-a-time consumption is what you actually want.

---

Next: [01-bridge-core.md](01-bridge-core.md)
